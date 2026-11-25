/** lenses, risks and constraints are written through the server's routes and drawn from its
 *  tree read */

import type { IssueTreeRead } from "../web/src/api/types";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import type { LineName } from "../web/src/features/issue-page/lenses/LensLines";
import {
  CONTROL_LABELS,
  DECLARED_WORDS,
  LENS_ANSWERS,
  LENS_WORDS,
  NODE_TYPE_NAMES,
  type Preset,
  sentenceCase,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { CHILD_PAGE } from "../web/src/features/issue-page/tree-fold";
import type { Outline } from "../web/src/features/issue-page/tree-read/tree-read";
import { createComment, getIssueCoverage } from "./api";
import { nextFrames } from "./frames";
import { seedWholeIssue, type WholeIssueSeed } from "./issue-layout-seed";
import { requirementText } from "./issue-layout-seed-text";
import { gotoIssue, issuePage, issueRead, phase } from "./issue-page";
import {
  DESKTOP,
  type DrawnRow,
  drawnRows,
  expectOpeningLanded,
  focusRow,
  focusThePage,
  nodeRow,
  nodeRows,
  PHONE,
  parentOfRow,
  READER,
  rowOf,
  unfoldEveryBranch,
} from "./issue-page-g";
import { ownWords } from "./issue-page-j";
import {
  convertedGate,
  heldAnswer,
  owedUnless,
  type TreeAnswerWatch,
  treeAnswerOf,
  treeReadGate,
  treeReadRoute,
} from "./issue-page-t-tree";
import {
  drawnIdOf,
  drawsChildPageRow,
  findingGate,
  focusedRowReadId,
  heldTreeAnswer,
  invokeLens,
  kindFilterGate,
  type LensKind,
  type LensLine,
  type LensReadIds,
  type LensSeed,
  lensAct,
  lensCatalogue,
  lensChainEntries,
  lensChipLinks,
  lensIdsIn,
  lensLines,
  lensMenu,
  lensMenuLine,
  lensReadIds,
  lensRows,
  lensShowChain,
  menuItems,
  seedLenses,
  treePagesOf,
} from "./issue-page-w";
import { expect, type Locator, type Page, type Route, test } from "./test";
import {
  type ApiRequest,
  type ApiRequests,
  expectLoadSettled,
  type RouteCount,
  watchApiRequests,
} from "./traffic";
import { assertPageFits } from "./unclipped";

/**
 * The browser rows of the lens rows of the new issue page: the rows of an issue's lenses,
 * findings, risks and constraints, the lines under them and under the statements a constraint
 * names, the chain a focused lens reads, and the Lens act that invokes a lens - read on the real
 * stack, in the two outlines that draw them: Conversation, at the issue's own address, and Graph
 * (`/issues/<key>/graph`). Their titles and their messages are the ones the issue's browser
 * checks name; a reading in Graph says so first in its message ("in Graph, ...", `inView`).
 *
 * Each row turns the new page on for alice with `phase` (`e2e/issue-page.ts`), seeds
 * `seedWholeIssue` (`e2e/issue-layout-seed.ts`) and, on it, `seedLenses`, opens the page with
 * `gotoIssue`, and reads it through the readers of `e2e/issue-page-g.ts`, of `e2e/issue-page-w.ts`
 * and, for a row's own words, `e2e/issue-page-j.ts`'s `ownWords`. A `@phone` row reads 1440 px on
 * `chromium` and 390 px on `iphone` (`rowSize`).
 *
 * Every node a row names, it names by the id the page's own tree read gave it
 * (`e2e/issue-page-t-tree.ts`'s `treeAnswerOf`, read through `lensReadIds`), and every row it reads,
 * by the id the page drew it under for that read id (`drawnIdOf`): no row builds an id. Rows 1 to
 * 3 and the two rows of row 2's clauses take those ids from Conversation's read, which holds both
 * constraints only while the base answers it as the whole tree, as this base does (the row owed
 * on the filter by kind, below); row 4 and that owed row take them from Graph's, which is the
 * whole tree on every base.
 *
 * What each row reads, and in which outline:
 *
 * - Row 1 reads in Conversation and again in Graph (`readLensRowsIn`), each line while the focus
 *   is on another row: (2) and (1), the two lenses under statement 1 after its other children,
 *   each with its tag, alice, its time and its verdict; (10) the threat model's tag of 129
 *   characters under statement 4, with the page fitting its width; (3) the Triggered by and
 *   Triggered chips, and `f` and Enter on each, which focus the other row in the same view with
 *   no new load; (4) the finding; (5) the risks' lines and check 1's chip, which focuses check 1;
 *   (6) the chain; the page's own words; (9) no address and no uuid in the page's text; and (7)
 *   `?node=` on the view's own address, with risk B's id and with a uuid nobody holds. It reads
 *   (8), that no filter draws these rows, once.
 * - Row 2 invokes in Conversation from statement 2's menu as its check says, from the row of each
 *   kind of node the route takes, and on statement 3, which holds `CHILD_PAGE` lenses written
 *   before the load (`seedPagedLensIssue`): the new lens's row stands past the first `CHILD_PAGE`
 *   of statement 3's children, focused and on screen. In Graph it invokes once, from statement
 *   2's menu, and lands on Graph's own address of the new lens. No filter offers the act.
 * - Row 3 compares a load of Conversation and a load of Graph of the issue with lenses with the
 *   same loads of an issue without them; presses `j`, the chain and the menu in Conversation; and
 *   invokes once in each outline, which sends one read of the tree, the open view's: in
 *   Conversation the read with `kind`, in Graph the read with no kind and no view.
 * - Row 4 reads in Graph the constraints at the top level, statement 5's and statement 6's
 *   chips, and Enter on the standard's chip, which focuses the standard's row in place; the same
 *   two chips in Conversation; statement 5's chip under Spec, which opens Graph at the standard's
 *   row; and `?node=` with the measurement's id on Graph's address.
 *
 * Each row checks its gates first, each with `owedUnless` (`e2e/issue-page-t-tree.ts`), and skips
 * where the server change a gate probes for is not on the base, with an `owed` annotation naming
 * what it waits on; it never passes then, so a row that could not run is never read as a pass.
 * Every row waits on the server serving the tree read, which `treeReadGate` reads as the read's
 * 404, and on the issue's conversion, the read's `converted: true` (`convertedGate`). These rows
 * carry no `@owed`: each runs for real once its gates are on the base. Row 2's clause on a
 * finding waits as well on the node model's commit 5c85ffdc1b, which stands a lens invoked on a
 * finding under it (`findingGate`, `e2e/issue-page-w.ts`): it is a row of its own, tagged
 * `@owed`, which checks the three gates the same way and asserts the clause once they are met.
 * Its title, "a finding's own row invokes a lens @owed", matches the `-g` of no check, so the
 * checks' runs, which invert no `@owed`, leave it out, and it stays out after its gates are met
 * too. The round that lands 5c85ffdc1b on the base gives the row back a title c-premortem's `-g`
 * matches, or adds the row to that pattern, and takes off `@owed`; until that round, no check
 * reads the clause.
 *
 * The readings of Conversation that wait on the model's filter of the tree read by kind are a
 * row of their own in the same way, "Conversation's own read draws no constraint row and opens
 * Graph for one @owed". Conversation's read names its kinds (`?kind=`), none of which is a
 * constraint, but this base answers that read as the whole tree, so Conversation draws the
 * issue's constraints as rows, and a chip or a `?node=` that names one stays in Conversation. The
 * row checks `treeReadGate`, `convertedGate` and `kindFilterGate` (`e2e/issue-page-w.ts`), which
 * is met only where the tree read with `kind=ask` answers no node of the issue of another kind
 * than an ask, but the issue's own node and the context that places the asks. Once the three are
 * met, it reads that Conversation draws neither constraint as a row, that statement 5's chip in
 * Conversation opens Graph with the standard's row focused, and that `?node=` with the
 * measurement's id on the issue's own address opens Graph with it focused. Its title matches the
 * `-g` of no check, c-constraints' (`a constraint stands under the issue`) above all, so no check
 * reads these clauses until the round that lands the filter on the base gives the row a title
 * that pattern matches, or adds the row to it, and takes off `@owed`.
 *
 * A row of its own, "a person invokes a lens from a link a row's words hold, through a read of
 * the tree", proves that the link statement 2's words draw keeps the focus, and the row its Lens
 * act, through the read of the tree a comment's event makes, and that a lens is invoked from
 * there; without `RowText.tsx`'s keeping of the words a read hands again in new objects
 * (decision 385), that read paints them again, the focus drops to `<body>`, and the row fails on
 * `the link in statement 2's words does not keep the focus through a read of the tree again`.
 *
 * At a base that serves the tree read and holds none of the lens rows, each of rows 1 to 4 fails
 * on an assertion of its own, at the end of a 30 s poll and not at its deadline. Rows 1, 2 and 4
 * read first, of what the lens rows draw, the assertion their checks name, and fail there: row 1
 * on `the pre-mortem stands under statement 1` and row 2 on `the focused row offers the Lens act`,
 * each in Conversation, and row 4 on `the constraint stands at the top level`, in Graph. Row 3
 * reads first the requests a load sends, which hold at such a base too, since it reads no lens
 * either, and fails on `the page draws no row for the pre-mortem`.
 *
 * That base is the first half of a before-and-after run: a `git worktree` at the branch's
 * merge-base, with these test files and their helpers (`e2e/issue-page-w.ts`) copied in, and
 * with them this change's `page-words.ts`, which holds the words these files read
 * (`LENS_WORDS`, `LENS_ANSWERS` and the names of the four row types) and draws nothing. These
 * files import no module this change adds but for a type (`LineName`), which loads nothing, so
 * they load at that base.
 *
 * One departure from the checks' words, said where it stands: the write layer tries a 5xx again
 * under its key until the reader stops it or the key's day is spent (`writeUnderKey`), so row 2's
 * refused invocation answers its first attempt 500 and every later one 400, the refusal the page
 * does not try again.
 */

/** The four views, none of which draws a lens, a finding, a risk or a constraint. */
const VIEWS = ["spec", "verification", "plan", "decisions"] as const satisfies readonly Preset[];

/** The two outlines, which draw them: Conversation, at the issue's own address, and Graph. */
const OUTLINES = ["conversation", "graph"] as const satisfies readonly Outline[];

/** The lens catalogue's read, which the menu of lens kinds sends as it opens. */
const CATALOGUE_READ = "**/api/v1/schema/blocks";

/** The invocation of a lens (`POST /api/v1/issues/{key}/lenses`). */
const INVOKE_ROUTE = "**/api/v1/issues/*/lenses";

/** The reads the page's shell sends on a timer of its own, whatever an act does. The credential
 *  requests waiting on the reader are read on every page, as it loads and every 15 s after, by
 *  the shell's Inbox count (`web/src/features/credentials/pending.ts`'s
 *  `CREDENTIAL_POLL_INTERVAL_MS`, `inbox/useInboxCount.ts`, `app.tsx`). The agents are read every
 *  15 s by the shell's poller (`refs/agent-registry.ts`' `AgentRegistryPoller`,
 *  `AGENT_REGISTRY_POLL_MS`), but only while something on the page draws an agent's name
 *  (`refs/AgentName.tsx` calls `useAgentRegistry(true)`), as the seeded issue's rows by agent
 *  sessions do (`e2e/issue-layout-seed.ts`). The rows here write as a person, so no lens row of
 *  theirs draws an agent's name and starts the agents read. A count of the requests an act sends
 *  leaves both reads out, since a window that spans one says nothing of the act. */
const SHELL_TIMED_READS: Readonly<Record<string, true>> = {
  "/api/v1/agents": true,
  "/api/v1/credential-requests": true,
};

/** `counts` with the shell's timed reads left out (`SHELL_TIMED_READS`): a load that lasts past
 *  the shell's next tick reads them once more, which says nothing of the load's own requests. */
function ownRoutes(counts: readonly RouteCount[]): readonly RouteCount[] {
  return counts.filter((one) => SHELL_TIMED_READS[one.route] !== true);
}

/** A uuid in any text the page draws. */
const UUID = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/giu;

/** The issue `seedWholeIssue` makes, with what `seedLenses` wrote on it. */
interface LensIssue {
  readonly lenses: LensSeed;
  readonly whole: WholeIssueSeed;
}

async function seedLensIssue(): Promise<LensIssue> {
  const whole = await seedWholeIssue();
  return { lenses: await seedLenses(whole.key), whole };
}

/** `seedLensIssue`, then `CHILD_PAGE` trade-off analyses more on statement 3, written one after
 *  another through the API as alice (`invokeLens`): as many children as the outline draws of a
 *  node before the row that stands for the rest (`tree-fold.ts`), which statement 3's other
 *  children take it past, so a lens invoked on statement 3 after them stands past that page. */
async function seedPagedLensIssue(): Promise<LensIssue> {
  const issue = await seedLensIssue();
  for (let written = 0; written < CHILD_PAGE; written += 1) {
    await invokeLens(issue.whole.key, { kind: "trade-off", on: issue.lenses.statements.three });
  }
  return issue;
}

/** The size a `@phone` row reads: 390 px on the `iphone` project, 1440 px on every other. */
function rowSize(): { readonly height: number; readonly width: number } {
  return test.info().project.name === "iphone" ? PHONE : DESKTOP;
}

/** `message` as a reading in `view` says it: as the checks name it in Conversation, where each
 *  row reads first, and after "in Graph," in Graph, so a failure names the outline it read. */
function inView(view: Outline, message: string): string {
  return view === "conversation" ? message : `in Graph, ${message}`;
}

/** Waits until the page stands at `view`'s own address of issue `key`, whatever its query, and
 *  fails with `message` where it stands at another. */
async function expectAt(page: Page, key: string, view: Outline, message: string): Promise<void> {
  const path = presetPath(key, view);
  await expect(page, message).toHaveURL((url) => url.pathname === path);
}

/** The kind of the catalogue a lens was invoked as, by the id its 201 names: the server answers
 *  a lens's `kind` as the catalogue's id of the kind. */
function kindNamed(kinds: readonly LensKind[], kind: string): LensKind {
  const found = kinds.find((one) => one.id === kind);
  if (found === undefined) throw new Error(`the lens catalogue holds no kind ${kind}`);
  return found;
}

/** The id the page draws the node `readId` names under, once it draws it: a row drawn now is
 *  its answer, and while none is, each poll first opens every branch a fold left shut, which
 *  ends with the focus on no row. A row a chip's Enter landed on keeps the focus the landing gave
 *  it (`focusThePage`), so a row already drawn is read without opening anything. It fails with
 *  `message`, by default that the page draws no row for `what`. */
async function drawnId(
  page: Page,
  readId: string,
  what: string,
  message = `the page draws no row for ${what}`
): Promise<string> {
  // A row the page draws stays drawn until a fold shuts it, which nothing here presses.
  const drawn = await drawnIdOf(page, readId);
  if (drawn !== null) return drawn;
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        return drawnIdOf(page, readId);
      },
      { message, timeout: 30_000 }
    )
    .not.toBeNull();
  const id = await drawnIdOf(page, readId);
  if (id === null) throw new Error(message);
  return id;
}

/** `drawnId` in `view`, failing with a message that names the outline (`inView`). */
function drawnIn(page: Page, view: Outline, readId: string, what: string): Promise<string> {
  return drawnId(page, readId, what, inView(view, `the page draws no row for ${what}`));
}

/** The row the drawn row of `readId` sits under (`e2e/issue-page-g.ts`'s `parentOfRow`): null at
 *  the top level, and undefined while the page draws no row for it. */
async function drawnParentOf(page: Page, readId: string): Promise<string | null | undefined> {
  const id = await drawnIdOf(page, readId);
  return id === null ? undefined : parentOfRow(page, id);
}

/** The rows drawn directly under the row `id`, in order, by the tree's own `aria-level`. */
function childrenOf(rows: readonly DrawnRow[], id: string): DrawnRow[] {
  const at = rows.findIndex((row) => row.id === id);
  if (at === -1) return [];
  const level = Number(rows[at]?.level ?? "");
  const children: DrawnRow[] = [];
  for (const row of rows.slice(at + 1)) {
    const depth = Number(row.level ?? "");
    if (depth <= level) break;
    if (depth === level + 1) children.push(row);
  }
  return children;
}

/** One lens line as a row reads it: its label, its stored words, and each chip by its words and
 *  the read id its link names (`?node=`), or null for a chip drawn as words alone. */
interface LineReading {
  readonly label: string;
  readonly targets: readonly { readonly node: string | null; readonly words: string }[];
  readonly words: string;
}

/** The line `line` of `lines`, or undefined where the row draws none. */
function lineReading(lines: readonly LensLine[], line: LineName): LineReading | undefined {
  const found = lines.find((one) => one.line === line);
  return found === undefined
    ? undefined
    : {
        label: found.label,
        targets: found.chips.map((chip) => ({
          node:
            chip.href === null
              ? null
              : new URL(chip.href, "http://page.invalid").searchParams.get("node"),
          words: chip.words,
        })),
        words: found.words,
      };
}

/** How many times `pressFTo` presses `f` before it gives up: past every stop a row draws. */
const STOPS_AT_MOST = 40;

/** Presses `f` until `target` holds the focus, as a reader steps through the focused row's links
 *  and controls (`useTreeKeymap.ts`'s `rowStops`), and fails with `message` where none does. */
async function pressFTo(page: Page, target: Locator, message: string): Promise<void> {
  await expect(target.first(), message).toBeAttached();
  for (let pressed = 0; pressed < STOPS_AT_MOST; pressed += 1) {
    // `f` focuses the next stop inside its own keydown handler, so this read answers for it.
    if (await target.first().evaluate((element) => element === document.activeElement)) return;
    await page.keyboard.press("f");
  }
  await expect(target.first(), message).toBeFocused();
}

/** Marks the page's document, so a row can tell that a press loaded no other. */
async function markDocument(page: Page): Promise<void> {
  await page.evaluate(() => {
    Reflect.set(window, "lensRowsDocument", true);
  });
}

/** Whether the page still shows the document `markDocument` marked. */
function sameDocument(page: Page): Promise<boolean> {
  return page.evaluate(() => Reflect.get(window, "lensRowsDocument") === true);
}

/** Item `place` of the open menu of lens kinds. */
function menuItem(page: Page, place: number): Locator {
  return lensMenu(page).getByRole("menuitem").nth(place);
}

/** The one lens the newest read holds that `before` did not, once a read holds it. */
async function newLens(
  tree: TreeAnswerWatch,
  before: ReadonlySet<string>,
  what: string
): Promise<string> {
  const answer = await heldTreeAnswer(tree, `the tree read after ${what}`, (held) =>
    lensIdsIn(held).some((id) => !before.has(id))
  );
  const made = lensIdsIn(answer).filter((id) => !before.has(id));
  expect(made, `${what} made more than one lens`).toHaveLength(1);
  return made[0] ?? "";
}

/**
 * Opens `view` of issue `key` at its own address of the node `readId` names (`?node=`) and waits
 * until its landing has put the focus on that row. The landing takes the focus back whenever
 * nothing holds it until the reader's first act (`IssueTree.tsx`), so this acts as a reader with
 * a Shift, which no key of the page binds, and the row may then leave the focus for the page
 * itself.
 */
async function landOn(
  page: Page,
  key: string,
  view: Outline,
  readId: string,
  what: string
): Promise<void> {
  await gotoIssue(page, `${presetPath(key, view)}?node=${encodeURIComponent(readId)}`);
  await expect
    .poll(() => focusedRowReadId(page), {
      message: inView(view, `the address of ${what} does not land on its row`),
      timeout: 30_000,
    })
    .toBe(readId);
  await page.keyboard.press("Shift");
}

/**
 * Invokes the catalogue's first kind on the node `target` names through its row's menu in `view`
 * of issue `key`, as a reader does - the focus on the row, `f` to the Lens act, Enter, Enter on
 * the first kind - and reads the new lens's row: focused, under the row of `target`, and at the
 * view's own address of the new lens (`?node=`). The row is reached by its own address in the
 * view (`landOn`), unless the caller `landed` on it already: below 1024 px the outline a landing
 * roots at its node's parent, the last invocation's included, may draw no row for `target`.
 * `what` names the node as the messages name it ("the open question", "statement 2"). Answers
 * the new lens's read id.
 */
async function invokeThroughMenu(
  page: Page,
  tree: TreeAnswerWatch,
  key: string,
  view: Outline,
  target: string,
  what: string,
  landed = false
): Promise<string> {
  if (!landed) await landOn(page, key, view, target, what);
  const held = await heldAnswer(tree, "the tree read");
  expect(
    (held.nodes ?? []).some((node) => node.id === target),
    inView(view, `the tree read does not hold ${what}`)
  ).toBe(true);
  const before = new Set(lensIdsIn(held));
  const targetId = await drawnIn(page, view, target, what);
  const row = await focusRow(page, targetId);
  await pressFTo(page, lensAct(row), inView(view, `${what}'s row offers no Lens act`));
  await page.keyboard.press("Enter");
  await expect(
    menuItem(page, 0),
    inView(view, `the Lens menu on ${what} lists no kind`)
  ).toBeFocused();
  await page.keyboard.press("Enter");
  const lens = await newLens(tree, before, `a lens on ${what}`);
  await expect
    .poll(() => focusedRowReadId(page), {
      message: inView(view, `the lens invoked on ${what} does not hold the focus`),
    })
    .toBe(lens);
  // The focus is the landing's on the new lens's address, which takes it back whenever nothing
  // holds it until the reader acts (`IssueTree.tsx`): a Shift, which no key of the page binds.
  await page.keyboard.press("Shift");
  const path = presetPath(key, view);
  await expect(
    page,
    inView(view, `the pick on ${what} does not land on the view's own address of the new lens`)
  ).toHaveURL((url) => url.pathname === path && url.searchParams.get("node") === lens);
  expect(
    await drawnParentOf(page, lens),
    inView(view, `the lens invoked on ${what} does not stand under it`)
  ).toBe(targetId);
  return lens;
}

/** Puts the focus on each row a view draws in turn - each of `ids` alone, where they are given -
 *  and fails where one offers the Lens act: the act is the outlines' alone. */
async function expectNoLensAct(
  page: Page,
  key: string,
  view: Preset,
  ids?: readonly string[]
): Promise<void> {
  await gotoIssue(page, presetPath(key, view));
  await expect
    .poll(() => nodeRows(page).count(), { message: `the ${view} view draws no row` })
    .toBeGreaterThan(0);
  // A view's rows come from one read, merged whole before it draws, so none come later.
  const drawn = await drawnRows(page);
  const focused =
    ids ?? new Set(drawn.flatMap((row) => (row.visible && row.id !== null ? [row.id] : [])));
  for (const id of focused) {
    const row = nodeRow(page, id).first();
    await expect(row, `the ${view} view draws no row ${id}`).toHaveCount(1);
    await row.focus();
    await expect(row, `the ${view} view's row ${id} takes no focus`).toBeFocused();
    // A row draws its acts in the frame after it takes the focus.
    await nextFrames(page);
    await expect(
      lensAct(issuePage(page)),
      `the ${view} view's row ${id} offers the Lens act`
    ).toHaveCount(0);
  }
}

/** The reads of a lens (`GET /api/v1/lenses?on=`) `requests` hold, by the address each names. */
function chainReads(requests: readonly ApiRequest[]): (string | null)[] {
  return requests.flatMap((request) =>
    request.method === "GET" && request.route === "/api/v1/lenses"
      ? [new URL(request.path, "http://page.invalid").searchParams.get("on")]
      : []
  );
}

/** The reads of the lens catalogue (`GET /api/v1/schema/blocks`) `requests` hold. */
function catalogueReads(requests: readonly ApiRequest[]): string[] {
  return requests.flatMap((request) =>
    request.route === "/api/v1/schema/blocks" ? [request.path] : []
  );
}

// ---------------------------------------------------------------------------------------------
// Row 1's readings, each in one outline.
// ---------------------------------------------------------------------------------------------

/** What row 1 reads in each outline: the issue, the read ids of what `seedLenses` wrote on it,
 *  and the words of the kinds its lenses were invoked as, as the catalogue holds them. */
interface LensRowsRead {
  readonly ids: LensReadIds;
  readonly key: string;
  readonly lenses: LensSeed;
  readonly words: {
    readonly preMortem: string;
    readonly threatModel: string;
    readonly tradeOff: string;
  };
}

/**
 * Row 1's readings of what the lens rows draw in `view`, on the page its load of the view left:
 * (2) and (1), (10), (3), (4), (5), (6), the page's own words, (9), and last (7), which loads the
 * view's own address again.
 */
async function readLensRowsIn(
  page: Page,
  traffic: ApiRequests,
  read: LensRowsRead,
  view: Outline
): Promise<void> {
  await expectLensesUnderStatementOne(page, read, view);
  await expectLongKind(page, read, view);
  await expectTriggerChips(page, read, view);
  await expectFinding(page, read, view);
  await expectRisks(page, read, view);
  await expectChain(page, read, view);
  await expectOwnWords(page, read, view);
  await expectNoAddress(page, view);
  await expectNodeAddress(page, traffic, read, view);
}

/**
 * Row 1's (2), the first reading of what the lens rows draw: statement 1's row is drawn, and the
 * pre-mortem's row stands under it, by the tree's own levels. Then (1): after statement 1's other
 * children, the trade-off analysis, then the pre-mortem, each tagged by its kind's words,
 * capitalised and whole, with the focus on another row; each by alice, with its time and its
 * verdict, or the words that say it has none.
 */
async function expectLensesUnderStatementOne(
  page: Page,
  read: LensRowsRead,
  view: Outline
): Promise<void> {
  const { ids, lenses, words } = read;
  const statementOneRead = ids.statements[0] ?? "";
  await expect
    .poll(
      async () => {
        await unfoldEveryBranch(page);
        const statement = await drawnIdOf(page, statementOneRead);
        return statement !== null && (await drawnParentOf(page, ids.preMortem)) === statement;
      },
      { message: inView(view, "the pre-mortem stands under statement 1"), timeout: 30_000 }
    )
    .toBe(true);
  const statementOne = await drawnIn(page, view, statementOneRead, "statement 1");
  const tradeOff = await drawnIn(page, view, ids.tradeOff, "the trade-off analysis");
  const preMortem = await drawnIn(page, view, ids.preMortem, "the pre-mortem");
  const rows = await drawnRows(page);
  expect(
    childrenOf(rows, statementOne)
      .slice(-2)
      .map((row) => row.id),
    inView(
      view,
      "under statement 1, after its other children, the trade-off analysis and then the pre-mortem"
    )
  ).toEqual([tradeOff, preMortem]);
  // The focus goes to statement 1, so each lens's tag is read on a row that has none.
  await focusRow(page, statementOne);
  for (const lens of [
    {
      id: tradeOff,
      kind: words.tradeOff,
      verdict: LENS_WORDS.no_verdict,
      what: "the trade-off analysis",
    },
    {
      id: preMortem,
      kind: words.preMortem,
      verdict: lenses.preMortem.verdict ?? "",
      what: "the pre-mortem",
    },
  ]) {
    const drawn = rowOf(rows, lens.id);
    expect(
      drawn?.words ?? drawn?.firstLine,
      inView(view, `${lens.what} does not draw its verdict`)
    ).toBe(lens.verdict);
    expect(drawn?.author, inView(view, `${lens.what} does not name alice`)).toBe(READER);
    const row = nodeRow(page, lens.id).first();
    await expect(
      row.locator("[data-node-tag]"),
      inView(view, `${lens.what}'s tag is not its kind's words, capitalised and whole`)
    ).toHaveText(sentenceCase(lens.kind));
    await expect(
      row.locator("[data-node-time]"),
      inView(view, `${lens.what} draws no time`)
    ).toHaveText(/\S/u);
  }
}

/** Row 1's (10): under statement 4, the threat model's tag reads its kind's words, 129
 *  characters, capitalised and whole, while the focus is on statement 4, and the page is no
 *  wider than its viewport (`assertPageFits`), at 1440 px and on `iphone` at 390 px. */
async function expectLongKind(page: Page, read: LensRowsRead, view: Outline): Promise<void> {
  const { ids, words } = read;
  expect(words.threatModel, "the threat model's kind does not run to 129 characters").toHaveLength(
    129
  );
  const statementFour = await drawnIn(page, view, ids.statements[3] ?? "", "statement 4");
  const threatModel = await drawnIn(page, view, ids.threatModel, "the threat model");
  expect(
    await parentOfRow(page, threatModel),
    inView(view, "the threat model does not stand under statement 4")
  ).toBe(statementFour);
  await focusRow(page, statementFour);
  await expect(
    nodeRow(page, threatModel).first().locator("[data-node-tag]"),
    inView(view, "the threat model's tag is not its kind's 129 characters, capitalised and whole")
  ).toHaveText(sentenceCase(words.threatModel));
  await assertPageFits(page);
}

/** Row 1's (3): each lens names the other by a chip, its kind's words whole, read while the
 *  focus is on statement 1; `f` to each chip and Enter focus the other's row in the same view,
 *  with no new load. */
async function expectTriggerChips(page: Page, read: LensRowsRead, view: Outline): Promise<void> {
  const { ids, key, words } = read;
  const statementOne = await drawnIn(page, view, ids.statements[0] ?? "", "statement 1");
  const tradeOff = await drawnIn(page, view, ids.tradeOff, "the trade-off analysis");
  const preMortem = await drawnIn(page, view, ids.preMortem, "the pre-mortem");
  const tradeOffRow = nodeRow(page, tradeOff).first();
  const preMortemRow = nodeRow(page, preMortem).first();
  await focusRow(page, statementOne);
  expect(
    lineReading(await lensLines(preMortemRow), "triggered_by"),
    inView(view, "the pre-mortem does not read Triggered by with the trade-off analysis's chip")
  ).toEqual({
    label: LENS_WORDS.triggered_by,
    targets: [{ node: ids.tradeOff, words: sentenceCase(words.tradeOff) }],
    words: "",
  });
  expect(
    lineReading(await lensLines(tradeOffRow), "triggered"),
    inView(view, "the trade-off analysis does not read Triggered with the pre-mortem's chip")
  ).toEqual({
    label: LENS_WORDS.triggered,
    targets: [{ node: ids.preMortem, words: sentenceCase(words.preMortem) }],
    words: "",
  });
  await markDocument(page);
  await focusRow(page, preMortem);
  await pressFTo(
    page,
    lensChipLinks(preMortemRow, "triggered_by"),
    inView(view, "f does not reach the trade-off analysis's chip on the pre-mortem")
  );
  await page.keyboard.press("Enter");
  await expect
    .poll(() => focusedRowReadId(page), {
      message: inView(view, "Enter on the trade-off analysis's chip does not focus its row"),
    })
    .toBe(ids.tradeOff);
  await pressFTo(
    page,
    lensChipLinks(tradeOffRow, "triggered"),
    inView(view, "f does not reach the pre-mortem's chip on the trade-off analysis")
  );
  await page.keyboard.press("Enter");
  await expect
    .poll(() => focusedRowReadId(page), {
      message: inView(view, "Enter on the pre-mortem's chip does not focus its row"),
    })
    .toBe(ids.preMortem);
  expect(
    await sameDocument(page),
    inView(view, "a press on a lens's chip loaded the page again")
  ).toBe(true);
  await expectAt(page, key, view, inView(view, "a press on a lens's chip left the view"));
}

/** Row 1's (4): the finding stands under the trade-off analysis, tagged as a finding. */
async function expectFinding(page: Page, read: LensRowsRead, view: Outline): Promise<void> {
  const finding = await drawnIn(page, view, read.ids.finding, "the finding");
  const tradeOff = await drawnIn(page, view, read.ids.tradeOff, "the trade-off analysis");
  expect(
    await parentOfRow(page, finding),
    inView(view, "the finding does not stand under the trade-off analysis")
  ).toBe(tradeOff);
  await expect(
    nodeRow(page, finding).first().locator("[data-node-tag]"),
    inView(view, "the finding's tag is not the word for a finding")
  ).toHaveText(NODE_TYPE_NAMES.finding);
}

/** Row 1's (5): risk A's early warning and prevention, each with its words, and the check that
 *  prevents it, whose chip goes to the check; risk B, which nothing prevents, says so. Each line
 *  is read while the focus is on the pre-mortem, which both risks stand under: (3) left the
 *  address on the pre-mortem, which below 1024 px roots the outline at statement 1, so the
 *  pre-mortem has a row at both widths and statement 1 may have none. */
async function expectRisks(page: Page, read: LensRowsRead, view: Outline): Promise<void> {
  const { ids, lenses } = read;
  const riskA = await drawnIn(page, view, ids.riskA, "risk A");
  const riskB = await drawnIn(page, view, ids.riskB, "risk B");
  const riskARow = nodeRow(page, riskA).first();
  await focusRow(page, await drawnIn(page, view, ids.preMortem, "the pre-mortem"));
  const riskALines = await lensLines(riskARow);
  expect(
    lineReading(riskALines, "early_warning"),
    inView(view, "risk A does not read Early warning: with its words")
  ).toEqual({ label: LENS_WORDS.early_warning, targets: [], words: lenses.riskA.early_warning });
  expect(
    lineReading(riskALines, "prevention"),
    inView(view, "risk A does not read Prevention: with its words")
  ).toEqual({ label: LENS_WORDS.prevention, targets: [], words: lenses.riskA.prevention });
  // The chip names check 1 by its text, whole, which its own row draws, not by its stored source.
  const checkRow = rowOf(await drawnRows(page), await drawnIn(page, view, ids.check1, "check 1"));
  const checkWords = checkRow?.firstLine ?? checkRow?.words ?? "";
  expect(
    lineReading(riskALines, "prevented_by"),
    inView(view, "risk A does not read Prevented by with check 1's chip")
  ).toEqual({
    label: LENS_WORDS.prevented_by,
    targets: [{ node: ids.check1, words: checkWords }],
    words: "",
  });
  expect(
    lineReading(await lensLines(nodeRow(page, riskB).first()), "prevented_by_nothing"),
    inView(view, "risk B does not read Prevented by nothing")
  ).toEqual({ label: LENS_WORDS.prevented_by_nothing, targets: [], words: "" });
  await focusRow(page, riskA);
  await pressFTo(
    page,
    lensChipLinks(riskARow, "prevented_by"),
    inView(view, "f does not reach check 1's chip on risk A")
  );
  await page.keyboard.press("Enter");
  await expect
    .poll(() => focusedRowReadId(page), {
      message: inView(view, "Enter on check 1's chip does not focus check 1's row"),
    })
    .toBe(ids.check1);
}

/**
 * Row 1's (6), the pre-mortem's chain: one press reads it, and it draws the trade-off analysis,
 * then the pre-mortem, each with the whole words of statement 1, which both stand under, though
 * statement 1's own row draws one first line while its branch is folded and no row holds the
 * focus. Below 1024 px the outline an address names a node of is rooted at that node's parent
 * (`IssueTree.tsx`'s `on`), so after check 1's chip statement 1 has no row: its own address in
 * the view draws its row at both widths, and the pre-mortem's address draws the pre-mortem's.
 */
async function expectChain(page: Page, read: LensRowsRead, view: Outline): Promise<void> {
  const { ids, key, words } = read;
  const statementOneRead = ids.statements[0] ?? "";
  await landOn(page, key, view, statementOneRead, "statement 1");
  const statementOne = await drawnIn(page, view, statementOneRead, "statement 1");
  // The landing has drawn the tree its address opens, and only a reader's press moves a fold
  // after it: open from 1024 px up, where the outline draws three levels, shut below.
  if (rowOf(await drawnRows(page), statementOne)?.expanded === "true") {
    await nodeRow(page, statementOne)
      .first()
      .getByRole("button", { exact: true, name: CONTROL_LABELS.fold })
      .first()
      .click();
  }
  await focusThePage(page);
  await expect
    .poll(async () => rowOf(await drawnRows(page), statementOne)?.firstLine ?? null, {
      message: inView(view, "statement 1's folded row draws no first line"),
    })
    .not.toBeNull();
  const statementLine = rowOf(await drawnRows(page), statementOne)?.firstLine;
  await landOn(page, key, view, ids.preMortem, "the pre-mortem");
  const preMortemRow = nodeRow(
    page,
    await drawnIn(page, view, ids.preMortem, "the pre-mortem")
  ).first();
  await pressFTo(
    page,
    lensShowChain(preMortemRow),
    inView(view, "f does not reach Show the whole chain on the pre-mortem")
  );
  await page.keyboard.press("Enter");
  await expect
    .poll(async () => (await lensChainEntries(preMortemRow)).map((entry) => entry.kind), {
      message: inView(
        view,
        "Show the whole chain does not draw the trade-off analysis, then the pre-mortem"
      ),
    })
    .toEqual([words.tradeOff, words.preMortem]);
  await expect(
    preMortemRow.locator("[data-lens-chain] ol"),
    inView(view, "Show the whole chain draws more than one chain")
  ).toHaveCount(1);
  // Statement 1's folded row draws less than its whole words. The poll waits until both
  // chain entries' lines name statement 1 by its whole words.
  expect(statementLine, inView(view, "statement 1's folded row draws its whole words")).not.toBe(
    requirementText(1)
  );
  await expect
    .poll(async () => (await lensChainEntries(preMortemRow)).map((entry) => entry.line), {
      message: inView(
        view,
        "the chain does not name statement 1 by its whole words for both lenses"
      ),
    })
    .toEqual([requirementText(1), requirementText(1)]);
}

/** Every word of the page's own is declared, and a kind's words, which the catalogue stores,
 *  are drawn as stored words and never as the page's. */
async function expectOwnWords(page: Page, read: LensRowsRead, view: Outline): Promise<void> {
  const chrome = await ownWords(issuePage(page));
  expect(
    [...new Set(chrome.flatMap(wordsIn).filter((word) => !DECLARED_WORDS.includes(word)))],
    inView(view, "the lens rows draw words of the page's own that its list does not declare")
  ).toEqual([]);
  const kinds = [read.words.tradeOff, read.words.preMortem, read.words.threatModel];
  expect(
    chrome.filter((text) =>
      kinds.some((words) => text.replace(/\s+/gu, " ").toLowerCase().includes(words.toLowerCase()))
    ),
    inView(view, "a kind's words stand in the page's own words")
  ).toEqual([]);
}

/** Row 1's (9): no text the page draws reads an address or a uuid. */
async function expectNoAddress(page: Page, view: Outline): Promise<void> {
  const text = await issuePage(page).innerText();
  expect(text.match(/dispatch:\/\/\S*/gu) ?? [], inView(view, "the page draws an address")).toEqual(
    []
  );
  expect(text.match(UUID) ?? [], inView(view, "the page draws a uuid")).toEqual([]);
}

/** Row 1's (7): `?node=` with the id of risk B on the view's own address opens the view on risk
 *  B; with a uuid nobody holds, as the view's own address opens: the same rows in the same
 *  order, and the same focus. */
async function expectNodeAddress(
  page: Page,
  traffic: ApiRequests,
  read: LensRowsRead,
  view: Outline
): Promise<void> {
  const path = presetPath(read.key, view);
  const own = view === "conversation" ? "the issue's own address" : "Graph's own address";
  await gotoIssue(page, `${path}?node=${encodeURIComponent(read.ids.riskB)}`);
  await expect
    .poll(() => focusedRowReadId(page), {
      message: inView(view, "?node= with risk B's id does not open with risk B's row focused"),
    })
    .toBe(read.ids.riskB);
  await gotoIssue(page, path);
  await expectLoadSettled(traffic);
  await expectOpeningLanded(page);
  const plainFocus = await focusedRowReadId(page);
  const plainRows = (await drawnRows(page)).map((row) => row.id);
  await gotoIssue(page, `${path}?node=${crypto.randomUUID()}`);
  await expectLoadSettled(traffic);
  await expect(
    page.getByTestId("issue-column"),
    inView(view, "?node= with a uuid nobody holds draws no issue")
  ).toBeVisible();
  expect(
    await focusedRowReadId(page),
    inView(view, `?node= with a uuid nobody holds does not open as ${own}`)
  ).toBe(plainFocus);
  await expect
    .poll(async () => (await drawnRows(page)).map((row) => row.id), {
      message: inView(view, `?node= with a uuid nobody holds does not draw the rows of ${own}`),
    })
    .toEqual(plainRows);
}

// ---------------------------------------------------------------------------------------------
// Row 3's readings, each of one load or one invocation in one outline.
// ---------------------------------------------------------------------------------------------

/** A request's path as the read it belongs to names it: its path and its query, but the cursor
 *  that names one page of the read. */
function readOf(path: string): string {
  const url = new URL(path, "http://page.invalid");
  url.searchParams.delete("cursor");
  const query = url.searchParams.toString();
  return query === "" ? url.pathname : `${url.pathname}?${query}`;
}

/** The read of issue `key`'s tree `requests` hold the first request of (`readOf`), or undefined
 *  where they hold none: what a load sends the tree route. */
function treeReadOf(requests: readonly ApiRequest[], key: string): string | undefined {
  const matches = treeReadRoute(key);
  const first = requests.find(
    (request) => request.method === "GET" && matches(new URL(request.path, "http://page.invalid"))
  );
  return first === undefined ? undefined : readOf(first.path);
}

/** Opens `view` of issue `key` at its own address and waits out the load: its requests, which
 *  `traffic` holds from the navigation on, to a 1.5 s quiet window (`expectLoadSettled`), the
 *  page's opening, and the tree read, which it answers. */
async function loadView(
  page: Page,
  traffic: ApiRequests,
  tree: TreeAnswerWatch,
  key: string,
  view: Outline
): Promise<IssueTreeRead> {
  traffic.reset();
  await gotoIssue(page, presetPath(key, view));
  await expectLoadSettled(traffic);
  await expectOpeningLanded(page);
  return heldAnswer(tree, `the tree read of ${key}`);
}

/** Row 3's reading of a load of `view` of the issue `seedLenses` wrote on, `traffic` holding its
 *  requests: by route, the requests of the same load of an issue with no lens (`without`), the
 *  shell's own timed reads left out; no read of a lens or of the lens catalogue; and no address
 *  of a lens, a finding, a risk or a constraint in the batched reference read. */
function expectLoadReadsNoLens(
  traffic: ApiRequests,
  without: readonly RouteCount[],
  lenses: LensSeed,
  view: Outline
): void {
  expect(
    ownRoutes(traffic.byRoute()),
    inView(view, "the load with lenses sends the requests of the load without them")
  ).toEqual(without);
  expect(chainReads(traffic.requests), inView(view, "the load sends a read of a lens")).toEqual([]);
  expect(
    catalogueReads(traffic.requests),
    inView(view, "the load reads the lens catalogue")
  ).toEqual([]);
  const addresses = [
    lenses.tradeOff.address,
    lenses.preMortem.address,
    lenses.finding.address,
    lenses.riskA.address,
    lenses.riskB.address,
    lenses.threatModel.address,
    lenses.standard.address,
    lenses.measurement.address,
    lenses.catalogueConstraint,
  ];
  expect(
    traffic.requests.flatMap((request) => {
      const url = new URL(request.path, "http://page.invalid");
      return url.pathname === "/api/v1/reference-targets"
        ? url.searchParams
            .getAll("ref")
            .filter(
              (ref) =>
                addresses.includes(ref) || /^dispatch:\/\/(lens|risk|constraint)\//u.test(ref)
            )
        : [];
    }),
    inView(view, "the batched reference read names a lens, a finding, a risk or a constraint")
  ).toEqual([]);
}

/**
 * Row 3's invocation in `view`, with the menu of lens kinds open on the pre-mortem's row and the
 * focus on its first kind: Enter invokes it, and the new lens holds the focus. It sends its write
 * and one read of the tree, one request for each page of it, and that read is the open view's:
 * each page's request is the read `load` names, the one the view's load sent the tree route
 * (`treeReadOf`), but for its cursor. The shell's own timed reads (`SHELL_TIMED_READS`) are left
 * out: a window that spans one says nothing of the invocation, and every other request in it is
 * counted. `before` holds every lens the tree read held before it. Answers the new lens's id.
 */
async function expectInvocationReads(
  page: Page,
  traffic: ApiRequests,
  tree: TreeAnswerWatch,
  key: string,
  view: Outline,
  before: ReadonlySet<string>,
  load: string
): Promise<string> {
  const treePages = treePagesOf(page);
  traffic.reset();
  await page.keyboard.press("Enter");
  const made = await newLens(
    tree,
    before,
    view === "conversation"
      ? "the invocation on the pre-mortem"
      : "the invocation on the pre-mortem in Graph"
  );
  await expect
    .poll(() => focusedRowReadId(page), {
      message: inView(view, "the lens invoked on the pre-mortem does not hold the focus"),
    })
    .toBe(made);
  await expectLoadSettled(traffic);
  const read = await treePages();
  expect(
    traffic.requests.flatMap((request) =>
      SHELL_TIMED_READS[new URL(request.path, "http://page.invalid").pathname] === true
        ? []
        : [`${request.method} ${readOf(request.path)}`]
    ),
    inView(
      view,
      "an invocation sends more than its write and one read of the tree, the open view's"
    )
  ).toEqual([`POST /api/v1/issues/${key}/lenses`, ...read.map(() => `GET ${load}`)]);
  expect(
    read.map((one) => one.cursor),
    inView(view, "the tree read an invocation sends asks for a page its read did not name")
  ).toEqual([null, ...read.slice(0, -1).map((one) => one.next)]);
  expect(
    read.at(-1)?.next,
    inView(view, "the tree read an invocation sends stops before its last page")
  ).toBe(null);
  return made;
}

// ---------------------------------------------------------------------------------------------
// Row 4's readings of the chips of the statements a constraint names, each in one outline.
// ---------------------------------------------------------------------------------------------

/** Row 4's (2) in `view`: statement 5 reads "Constrained by" with the standard's chip, its words
 *  whole, its link naming the standard's read id. Answers the id statement 5 is drawn under. */
async function expectStandardChip(
  page: Page,
  ids: LensReadIds,
  lenses: LensSeed,
  view: Outline
): Promise<string> {
  const statementFive = await drawnIn(page, view, ids.statements[4] ?? "", "statement 5");
  expect(
    lineReading(await lensLines(nodeRow(page, statementFive).first()), "constrained_by"),
    inView(view, "statement 5 does not read Constrained by with the standard's chip")
  ).toEqual({
    label: LENS_WORDS.constrained_by,
    targets: [{ node: ids.standard, words: lenses.standard.standard ?? "" }],
    words: "",
  });
  return statementFive;
}

/** Row 4's (3) in `view`: statement 6 names the catalogue's constraint by its standard, as words
 *  alone, since the catalogue's constraint is no row of the issue. */
async function expectCatalogueChip(
  page: Page,
  ids: LensReadIds,
  catalogueStandard: string,
  view: Outline
): Promise<void> {
  const statementSix = await drawnIn(page, view, ids.statements[5] ?? "", "statement 6");
  expect(
    lineReading(await lensLines(nodeRow(page, statementSix).first()), "constrained_by"),
    inView(
      view,
      "statement 6's chip does not read the catalogue constraint's standard as words alone"
    )
  ).toEqual({
    label: LENS_WORDS.constrained_by,
    targets: [{ node: null, words: catalogueStandard }],
    words: "",
  });
}

// Row 1.
// SLOW(>=2s): a whole seeded issue with its lenses, read in Conversation and again in Graph: in
// each, every branch opened, each lens and risk focused in turn, four loads by `?node=` and one
// of the outline's own address; then one load of each view.
test("lenses are drawn under their node with the chain that triggered them @phone", async ({
  browser,
}) => {
  test.setTimeout(420_000);
  const size = rowSize();
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    await page.setViewportSize(size);
    const tree = treeAnswerOf(page);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectOpeningLanded(page);
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    const ids = lensReadIds(answer, key, lenses);
    expect(
      answer.nodes?.find((node) => node.id === ids.tradeOff)?.parent?.id,
      "the tree read does not place the trade-off analysis under its first statement"
    ).toBe(ids.statements[0]);
    const kinds = await lensCatalogue();
    const read: LensRowsRead = {
      ids,
      key,
      lenses,
      words: {
        preMortem: kindNamed(kinds, lenses.preMortem.kind).words,
        threatModel: kindNamed(kinds, lenses.threatModel.kind).words,
        tradeOff: kindNamed(kinds, lenses.tradeOff.kind).words,
      },
    };

    // (1) to (7), (9) and (10) in Conversation, the issue's own address the page has loaded, and
    // again in Graph, loaded at its own.
    await readLensRowsIn(page, traffic, read, "conversation");
    await gotoIssue(page, presetPath(key, "graph"));
    await expectOpeningLanded(page);
    await readLensRowsIn(page, traffic, read, "graph");

    // (8) No view draws a lens, a finding, a risk or a constraint.
    for (const view of VIEWS) {
      await gotoIssue(page, presetPath(key, view));
      await expect
        .poll(() => nodeRows(page).count(), { message: `the ${view} view draws no row` })
        .toBeGreaterThan(0);
      await expect(
        lensRows(page),
        `the ${view} view draws a lens, a finding, a risk or a constraint`
      ).toHaveCount(0);
    }
  });
});

// Row 2.
// SLOW(>=2s): a whole seeded issue with its lenses and 50 lenses more on statement 3, nine
// invocations through the menu, one of them in Graph and one past statement 3's first page of
// children, a walk of every row of three views, a retried write and three loads that each read
// the catalogue.
test("a person invokes a lens from a node's menu @phone", async ({ browser }) => {
  test.setTimeout(480_000);
  const size = rowSize();
  await phase(browser, seedPagedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    const plain = presetPath(key, "conversation");
    await page.setViewportSize(size);
    const tree = treeAnswerOf(page);
    await gotoIssue(page, plain);
    await expectOpeningLanded(page);
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    const ids = lensReadIds(answer, key, lenses);
    const kinds = await lensCatalogue();
    expect(kinds, "the lens catalogue does not hold its 22 kinds").toHaveLength(22);
    // Statement 2's address, as the coverage read gives it (as `seedLenses` reads statement 1's):
    // the `on` its row sends.
    const statementTwoAddress = (await getIssueCoverage(key)).requirements?.[1]?.ref;
    expect(statementTwoAddress, "the coverage read holds no statement 2").toBeDefined();

    // The first reading of what the lens rows draw: statement 2's row, found by the read id it
    // draws, offers the Lens act once it holds the focus.
    const statementTwo = await drawnId(
      page,
      ids.statements[1] ?? "",
      "statement 2",
      "the focused row offers the Lens act"
    );
    const row = await focusRow(page, statementTwo);
    await pressFTo(page, lensAct(row), "the focused row offers the Lens act");

    // Enter opens the menu, which says it is reading the catalogue while that read is held, and
    // then lists every kind in the catalogue's order with its first source's standard.
    const catalogue = Promise.withResolvers<void>();
    const holdCatalogue = async (route: Route) => {
      await catalogue.promise;
      await route.continue();
    };
    await page.route(CATALOGUE_READ, holdCatalogue);
    try {
      await page.keyboard.press("Enter");
      await expect(
        lensMenu(page),
        "Enter on the Lens act does not open the menu of lens kinds"
      ).toHaveAccessibleName(LENS_WORDS.menu_title);
      await expect(
        lensMenuLine(page),
        "the menu does not say it is reading the lens kinds"
      ).toHaveText(LENS_ANSWERS.loading_kinds);
    } finally {
      catalogue.resolve();
    }
    await expect
      .poll(() => menuItems(lensMenu(page)), {
        message: "the menu does not list each kind in the catalogue's order with its standard",
      })
      .toEqual(
        kinds.map((kind) => ({ standard: kind.sources?.[0]?.standard ?? "", words: kind.words }))
      );
    await page.unroute(CATALOGUE_READ, holdCatalogue);
    await assertPageFits(page);
    await page.keyboard.press("Escape");
    await expect(row, "Escape does not give the focus back to statement 2's row").toBeFocused();
    await expect(lensMenu(page), "Escape leaves the menu open").toHaveCount(0);

    // Again: two down arrows and Enter pick the third kind, which says it is examining while the
    // write is held, and lands on the new lens's row, focused, under statement 2.
    const before = new Set(lensIdsIn(await heldAnswer(tree, "the tree read")));
    await pressFTo(page, lensAct(row), "the focused row offers the Lens act");
    await page.keyboard.press("Enter");
    await expect(
      menuItem(page, 0),
      "the menu does not put the focus on its first kind"
    ).toBeFocused();
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    await expect(menuItem(page, 2), "two down arrows do not reach the third kind").toBeFocused();
    const write = Promise.withResolvers<void>();
    const sent: unknown[] = [];
    const holdWrite = async (route: Route) => {
      if (route.request().method() === "POST") {
        sent.push(route.request().postDataJSON());
        await write.promise;
      }
      await route.continue();
    };
    await page.route(INVOKE_ROUTE, holdWrite);
    try {
      await page.keyboard.press("Enter");
      await expect(
        lensMenuLine(page),
        "the menu does not say it is examining the statement"
      ).toHaveText(LENS_ANSWERS.examining);
    } finally {
      write.resolve();
    }
    expect(sent, "the pick does not send the statement and the third kind alone").toEqual([
      { kind: kinds[2]?.id, on: statementTwoAddress },
    ]);
    const made = await newLens(tree, before, "the pick on statement 2");
    await page.unroute(INVOKE_ROUTE, holdWrite);
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "the lens invoked on statement 2 does not hold the focus",
      })
      .toBe(made);
    const madeId = await drawnIdOf(page, made);
    expect(madeId, "the page draws no row for the lens invoked on statement 2").not.toBeNull();
    expect(
      await parentOfRow(page, madeId ?? ""),
      "the lens invoked on statement 2 does not stand under it"
    ).toBe(statementTwo);
    expect(
      rowOf(await drawnRows(page), madeId ?? "")?.author,
      "the lens invoked on statement 2 does not name alice"
    ).toBe(READER);
    // The reload opens the landing's address, `?node=` with the new lens: the page focuses its
    // row with the path to it open, so statement 2's lenses are drawn with nothing unfolded.
    await page.reload();
    await issueRead(page);
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "after a reload the landing does not focus the lens invoked on statement 2",
        timeout: 30_000,
      })
      .toBe(made);
    await expect
      .poll(
        async () =>
          childrenOf(await drawnRows(page), statementTwo)
            .filter((child) => child.type === "lens")
            .map((child) => child.id)
            .at(-1),
        {
          message: "after a reload the lens invoked on statement 2 is not its last lens",
          timeout: 30_000,
        }
      )
      .toBe(madeId);

    // In Graph statement 2's row offers the Lens act too, and a pick there lands on the new
    // lens's row in Graph, focused, under statement 2, at Graph's own address of it (`?node=`).
    await invokeThroughMenu(page, tree, key, "graph", ids.statements[1] ?? "", "statement 2");

    // The act is the outlines' alone: no row of the four views offers it - statement 2's row of
    // the Spec view, and every row of the other three.
    for (const view of VIEWS) {
      await expectNoLensAct(page, key, view, view === "spec" ? [statementTwo] : undefined);
    }

    // Each kind of node the route takes by its address takes the act: an open question, a
    // comment, a message, a lens, a risk and a constraint.
    await gotoIssue(page, plain);
    await expectOpeningLanded(page);
    // The open question is an ask block of the spec: its node is the one holding that block.
    const question = answer.nodes?.find((node) => node.block_id === whole.openQuestion.blockId);
    if (question === undefined) throw new Error(`the tree read of ${key} holds no open question`);
    for (const [target, what] of [
      [question.id, "the open question"],
      [whole.comments.threads[0] ?? "", "the comment"],
      [whole.messages.root, "the message"],
      [ids.tradeOff, "the trade-off analysis"],
      [ids.riskA, "risk A"],
      [ids.standard, "the standard constraint"],
    ] as const) {
      await invokeThroughMenu(page, tree, key, "conversation", target, what);
    }

    // Statement 3 holds the `CHILD_PAGE` lenses `seedPagedLensIssue` wrote after its other
    // children, so the outline draws the first `CHILD_PAGE` of its children and a row that stands
    // for the rest. A pick on statement 3 draws the new lens's row past that page, focused
    // (`invokeThroughMenu`) and on screen: the landing's address names the new lens, and a node
    // an address names is drawn with every child before it (`tree-fold.ts`).
    const statementThreeRead = ids.statements[2] ?? "";
    await landOn(page, key, "conversation", statementThreeRead, "statement 3");
    const statementThree = await drawnId(page, statementThreeRead, "statement 3");
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          return {
            children: childrenOf(await drawnRows(page), statementThree).length,
            rest: await drawsChildPageRow(page, statementThree),
          };
        },
        {
          message: `statement 3 does not draw its first ${CHILD_PAGE} children and a row for the rest`,
          timeout: 30_000,
        }
      )
      .toEqual({ children: CHILD_PAGE, rest: true });
    const paged = await invokeThroughMenu(
      page,
      tree,
      key,
      "conversation",
      statementThreeRead,
      "statement 3",
      true
    );
    const pagedId = await drawnId(page, paged, "the lens invoked on statement 3");
    await expect(
      nodeRow(page, pagedId).first(),
      "the lens invoked on statement 3 is not on screen"
    ).toBeInViewport();
    // The landing drew the new lens's row, with every child of statement 3 before it, in the
    // frame that gave it the focus `invokeThroughMenu` waited for, so the rows drawn now hold them.
    expect(
      childrenOf(await drawnRows(page), statementThree).findIndex((child) => child.id === pagedId),
      `the lens invoked on statement 3 does not stand past the first ${CHILD_PAGE} of its children`
    ).toBeGreaterThanOrEqual(CHILD_PAGE);

    // A catalogue read the server refuses at every attempt: the menu says the kinds could not be
    // read. A new load, since the catalogue is read once in a load.
    const refuseCatalogue = async (route: Route) => {
      await route.fulfill({ json: { code: "INTERNAL", error: "boom" }, status: 500 });
    };
    await page.route(CATALOGUE_READ, refuseCatalogue);
    await gotoIssue(page, plain);
    await expectOpeningLanded(page);
    const refused = await focusRow(
      page,
      await drawnId(page, ids.statements[1] ?? "", "statement 2")
    );
    await pressFTo(page, lensAct(refused), "the focused row offers the Lens act");
    await page.keyboard.press("Enter");
    await expect(
      lensMenuLine(page),
      "a refused catalogue read does not say the lens kinds could not be read"
    ).toHaveText(LENS_ANSWERS.kinds_failed);
    await page.keyboard.press("Escape");
    await expect(refused, "Escape does not give the focus back to the row").toBeFocused();
    await page.unroute(CATALOGUE_READ, refuseCatalogue);

    // An invocation the server fails: the page tries a 5xx again under its key, so the first
    // attempt answers 500 and every later one 400, the refusal the write layer does not try
    // again. The row says it could not examine the node, as a status, and gains no lens.
    const lensesBefore = lensIdsIn(await heldAnswer(tree, "the tree read"));
    const rowsBefore = await lensRows(page).count();
    let attempts = 0;
    const failWrite = async (route: Route) => {
      if (route.request().method() !== "POST") {
        await route.continue();
        return;
      }
      attempts += 1;
      await route.fulfill({
        json: { code: attempts === 1 ? "INTERNAL" : "LENS_INPUT", error: "Dispatch refused it" },
        status: attempts === 1 ? 500 : 400,
      });
    };
    await page.route(INVOKE_ROUTE, failWrite);
    await pressFTo(page, lensAct(refused), "the focused row offers the Lens act");
    await page.keyboard.press("Enter");
    await expect(menuItem(page, 0), "the menu opened again lists no kind").toBeFocused();
    await page.keyboard.press("Enter");
    await expect(
      refused.getByRole("status").filter({ hasText: LENS_ANSWERS.failed }),
      "a failed invocation does not say, as a status, that it could not examine the node"
    ).toBeVisible();
    expect(attempts, "the page did not try the 500 again under its key").toBeGreaterThanOrEqual(2);
    await expect(refused, "the refusal does not give the focus back to the row").toBeFocused();
    expect(
      lensIdsIn(await heldAnswer(tree, "the tree read")),
      "a failed invocation made a lens"
    ).toEqual(lensesBefore);
    await expect(lensRows(page), "a failed invocation draws a row").toHaveCount(rowsBefore);
    await page.unroute(INVOKE_ROUTE, failWrite);

    // An invocation on a node the server no longer holds: the row says it is no longer here.
    const goneWrite = async (route: Route) => {
      if (route.request().method() !== "POST") {
        await route.continue();
        return;
      }
      await route.fulfill({ json: { code: "NOT_FOUND", error: "no such node" }, status: 404 });
    };
    await page.route(INVOKE_ROUTE, goneWrite);
    await pressFTo(page, lensAct(refused), "the focused row offers the Lens act");
    await page.keyboard.press("Enter");
    await expect(menuItem(page, 0), "the menu opened again lists no kind").toBeFocused();
    await page.keyboard.press("Enter");
    await expect(
      refused.getByRole("status").filter({ hasText: LENS_ANSWERS.gone }),
      "an invocation answered 404 does not say the node is no longer here"
    ).toBeVisible();
    await page.unroute(INVOKE_ROUTE, goneWrite);
  });
});

// Row 2's Lens act from a link a row's words hold, through a read of the tree again: the read a
// comment's event makes hands statement 2's words in new objects, which `RowText.tsx` keeps
// unpainted (decision 385), so the link keeps the focus and the row its Lens act.
// SLOW(>=2s): a whole seeded issue with its lenses, a comment written through the API and the
// tree read its event makes, and one invocation through the menu.
test("a person invokes a lens from a link a row's words hold, through a read of the tree", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    const tree = treeAnswerOf(page);
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectOpeningLanded(page);
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    const ids = lensReadIds(answer, key, lenses);
    const before = new Set(lensIdsIn(answer));

    // The focus on the one link statement 2's words draw, for the document they cite, reached
    // with `f` as a reader reaches it.
    const statementTwo = await drawnId(page, ids.statements[1] ?? "", "statement 2");
    const row = await focusRow(page, statementTwo);
    const link = row.locator("[data-node-words] a[data-row-reference]");
    await expect(link, "statement 2's words do not draw one link").toHaveCount(1);
    await pressFTo(page, link, "f does not reach the link in statement 2's words");

    // A comment written through the API raises an event the page reads the tree again for, and
    // the new comment's row drawn is that read drawn: every row is drawn again from it.
    const reread = page.waitForResponse(
      (response) =>
        response.request().method() === "GET" &&
        new URL(response.url()).pathname === `/api/v1/issues/${key}/tree`
    );
    const comment = await createComment(key, {
      body: "Written while the focus is on the link in statement 2's words.",
    });
    await reread;
    await expect
      .poll(() => drawnIdOf(page, comment.id), {
        message: "the page does not draw the comment it read the tree again for",
        timeout: 30_000,
      })
      .not.toBeNull();

    // The link keeps the focus and the row its Lens act, from which a person invokes a lens as
    // from the row: the new lens holds the focus, under statement 2.
    await expect(
      link,
      "the link in statement 2's words does not keep the focus through a read of the tree again"
    ).toBeFocused();
    await pressFTo(
      page,
      lensAct(row),
      "after a read of the tree again, statement 2's row offers no Lens act"
    );
    await page.keyboard.press("Enter");
    await expect(menuItem(page, 0), "the Lens menu on statement 2 lists no kind").toBeFocused();
    await page.keyboard.press("Enter");
    const lens = await newLens(tree, before, "the invocation from statement 2's link");
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "the lens invoked from statement 2's link does not hold the focus",
      })
      .toBe(lens);
    // The focus is the landing's on the new lens's address, which takes it back whenever nothing
    // holds it until the reader acts (`IssueTree.tsx`): a Shift, which no key of the page binds.
    await page.keyboard.press("Shift");
    expect(
      await drawnParentOf(page, lens),
      "the lens invoked from statement 2's link does not stand under it"
    ).toBe(statementTwo);
  });
});

// Row 2's Lens act on a finding, owed until the node model's commit 5c85ffdc1b is on the base:
// titled so that no check's `-g` matches it, above all the one that picks rows 1 and 2
// (`lenses are drawn under their node|a person invokes a lens`), since no project inverts
// `@owed` and that run does not either.
// SLOW(>=2s): a whole seeded issue with its lenses, the gate's invocation and its tree read,
// every branch opened and one invocation through the menu.
test("a finding's own row invokes a lens @owed", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    owedUnless(await findingGate(page, key, lenses));
    const tree = treeAnswerOf(page);
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectOpeningLanded(page);
    const answer = await heldAnswer(tree, `the tree read of ${key}`);
    await invokeThroughMenu(
      page,
      tree,
      key,
      "conversation",
      lensReadIds(answer, key, lenses).finding,
      "the finding"
    );
  });
});

// Row 3.
// SLOW(>=2s): two whole seeded issues each loaded at Conversation's address and at Graph's, each
// load waited out to a 1.5 s quiet window, then `j` over every row, two chain presses, two menu
// openings and an invocation in Conversation, and a menu opening and an invocation in Graph,
// each waited out to that window.
test("the first load adds no request for lenses and reads a chain only at a press", async ({
  browser,
}) => {
  test.setTimeout(300_000);
  const without: Record<Outline, readonly RouteCount[]> = { conversation: [], graph: [] };
  await phase(browser, seedWholeIssue, async (page, whole) => {
    owedUnless(await treeReadGate(page, whole.key));
    owedUnless(await convertedGate(page, whole.key));
    const tree = treeAnswerOf(page);
    const traffic = watchApiRequests(page);
    for (const view of OUTLINES) {
      await loadView(page, traffic, tree, whole.key, view);
      without[view] = ownRoutes(traffic.byRoute());
    }
  });
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    const tree = treeAnswerOf(page);
    const traffic = watchApiRequests(page);

    // Conversation's load, at the issue's own address: the requests of the load without lenses,
    // and a read of the tree that names its kinds and no view.
    const answer = await loadView(page, traffic, tree, key, "conversation");
    expectLoadReadsNoLens(traffic, without.conversation, lenses, "conversation");
    const conversationRead = treeReadOf(traffic.requests, key) ?? "";
    const conversationQuery = new URL(conversationRead, "http://page.invalid").searchParams;
    expect(
      { kind: conversationQuery.getAll("kind").length > 0, view: conversationQuery.has("view") },
      "Conversation's load reads the tree with no kind, or with a view"
    ).toEqual({ kind: true, view: false });
    const ids = lensReadIds(answer, key, lenses);

    // `j` over every row focuses each lens in turn, which reads nothing.
    await unfoldEveryBranch(page);
    // Every branch is open and the load has settled, so the rows drawn now are all of them.
    const count = await nodeRows(page).count();
    traffic.reset();
    await nodeRows(page).first().focus();
    for (let row = 1; row < count; row += 1) await page.keyboard.press("j");
    await expectLoadSettled(traffic);
    expect(chainReads(traffic.requests), "j over every row sends a read of a lens").toEqual([]);

    // One press on the pre-mortem's Show the whole chain reads its chain once; away, back and a
    // second press read nothing more.
    const preMortem = await drawnId(page, ids.preMortem, "the pre-mortem");
    const statementOne = await drawnId(page, ids.statements[0] ?? "", "statement 1");
    traffic.reset();
    for (const press of ["first", "second"]) {
      const preMortemRow = await focusRow(page, preMortem);
      await pressFTo(
        page,
        lensShowChain(preMortemRow),
        "f does not reach Show the whole chain on the pre-mortem"
      );
      await page.keyboard.press("Enter");
      await expect(
        preMortemRow.locator("[data-lens-chain-entry]"),
        `the ${press} press of Show the whole chain draws no chain of two lenses`
      ).toHaveCount(2);
      await expectLoadSettled(traffic);
      expect(
        chainReads(traffic.requests),
        `the ${press} press of Show the whole chain does not leave one read of the pre-mortem's chain`
      ).toEqual([lenses.preMortem.address]);
      await focusRow(page, statementOne);
    }

    // Two openings of the menu read the catalogue once.
    const preMortemRow = await focusRow(page, preMortem);
    traffic.reset();
    for (const opening of ["first", "second"]) {
      await pressFTo(page, lensAct(preMortemRow), "the pre-mortem's row offers no Lens act");
      await page.keyboard.press("Enter");
      await expect(
        menuItem(page, 0),
        `the ${opening} opening of the menu lists no kind`
      ).toBeFocused();
      if (opening === "first") await page.keyboard.press("Escape");
    }
    await expectLoadSettled(traffic);
    expect(
      catalogueReads(traffic.requests),
      "two openings of the menu read the lens catalogue more than once"
    ).toHaveLength(1);

    // An invocation in Conversation sends its write and one read of the tree, Conversation's.
    const made = await expectInvocationReads(
      page,
      traffic,
      tree,
      key,
      "conversation",
      new Set(lensIdsIn(answer)),
      conversationRead
    );

    // Graph's load, at its own address: the requests of the load without lenses, and a read of
    // the tree with no kind and no view.
    await loadView(page, traffic, tree, key, "graph");
    expectLoadReadsNoLens(traffic, without.graph, lenses, "graph");
    const graphRead = treeReadOf(traffic.requests, key) ?? "";
    expect(graphRead, "Graph's load reads the tree with a kind or a view").toBe(
      `/api/v1/issues/${key}/tree`
    );

    // An invocation in Graph, from the pre-mortem's menu, sends its write and one read of the
    // tree, Graph's. Its lenses before it are those of Graph's read, which holds Conversation's.
    const held = await heldTreeAnswer(tree, "Graph's tree read", (graphAnswer) =>
      lensIdsIn(graphAnswer).includes(made)
    );
    const graphPreMortem = await focusRow(
      page,
      await drawnIn(page, "graph", ids.preMortem, "the pre-mortem")
    );
    await pressFTo(
      page,
      lensAct(graphPreMortem),
      inView("graph", "the pre-mortem's row offers no Lens act")
    );
    await page.keyboard.press("Enter");
    await expect(
      menuItem(page, 0),
      inView("graph", "the opening of the menu lists no kind")
    ).toBeFocused();
    await expectInvocationReads(
      page,
      traffic,
      tree,
      key,
      "graph",
      new Set(lensIdsIn(held)),
      graphRead
    );
  });
});

// Row 4.
// SLOW(>=2s): a whole seeded issue with its lenses, loaded in Graph, in Conversation, under Spec
// and at Graph's `?node=`, every branch opened, and the standard's chip pressed in Graph and
// under Spec.
test("a constraint stands under the issue and a statement names it @phone", async ({ browser }) => {
  test.setTimeout(300_000);
  const size = rowSize();
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    const graph = presetPath(key, "graph");
    await page.setViewportSize(size);
    const tree = treeAnswerOf(page);
    // Graph's read is the whole tree, the issue's constraints in it, on every base: the row names
    // each node by the id that read gives it.
    await gotoIssue(page, graph);
    await expectOpeningLanded(page);
    const answer = await heldAnswer(tree, `Graph's tree read of ${key}`);
    const ids = lensReadIds(answer, key, lenses);

    // (1) In Graph the two constraints stand at the top level, in the read's order, each with
    // its words, its line, its tag, alice and its time.
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          return Promise.all([ids.standard, ids.measurement].map((id) => drawnParentOf(page, id)));
        },
        { message: "the constraint stands at the top level", timeout: 30_000 }
      )
      .toEqual([null, null]);
    const standard = await drawnId(page, ids.standard, "the standard constraint");
    const measurement = await drawnId(page, ids.measurement, "the measurement constraint");
    const rows = await drawnRows(page);
    expect(
      rows.flatMap((row) => (row.id === standard || row.id === measurement ? [row.id] : [])),
      "the two constraints are not drawn in the read's order"
    ).toEqual(
      (answer.nodes ?? []).flatMap((node) =>
        node.id === ids.standard ? [standard] : node.id === ids.measurement ? [measurement] : []
      )
    );
    for (const constraint of [
      {
        id: standard,
        lines: [
          { chips: [], label: "", line: "reference", words: lenses.standard.reference ?? "" },
        ],
        what: "the standard constraint",
        words: lenses.standard.standard ?? "",
      },
      {
        id: measurement,
        lines: [
          {
            chips: [],
            label: LENS_WORDS.taken_at,
            line: "taken_at",
            words: lenses.measurement.taken_where ?? "",
          },
        ],
        what: "the measurement constraint",
        words: `${lenses.measurement.measurement ?? ""}: ${lenses.measurement.value ?? ""}`,
      },
    ]) {
      const drawn = rowOf(rows, constraint.id);
      expect(drawn?.words, `${constraint.what} does not read its words`).toBe(constraint.words);
      expect(drawn?.author, `${constraint.what} does not name alice`).toBe(READER);
      const row = nodeRow(page, constraint.id).first();
      expect(await lensLines(row), `${constraint.what} does not draw its line`).toEqual(
        constraint.lines
      );
      await expect(
        row.locator("[data-node-tag]"),
        `${constraint.what}'s tag is not the word for a constraint`
      ).toHaveText(NODE_TYPE_NAMES.constraint);
      await expect(row.locator("[data-node-time]"), `${constraint.what} draws no time`).toHaveText(
        /\S/u
      );
    }

    // (2) In Graph statement 5 names the standard by a chip, its words whole, and Enter on the
    // chip focuses the standard's row in place: in Graph, with no new load.
    const graphFive = await expectStandardChip(page, ids, lenses, "graph");
    const graphFiveRow = nodeRow(page, graphFive).first();
    await markDocument(page);
    await focusRow(page, graphFive);
    await pressFTo(
      page,
      lensChipLinks(graphFiveRow, "constrained_by"),
      inView("graph", "f does not reach the standard's chip on statement 5")
    );
    await page.keyboard.press("Enter");
    await expect
      .poll(() => focusedRowReadId(page), {
        message: inView("graph", "Enter on the standard's chip does not focus the standard's row"),
      })
      .toBe(ids.standard);
    expect(
      await sameDocument(page),
      inView("graph", "Enter on the standard's chip loaded the page again")
    ).toBe(true);
    await expectAt(page, key, "graph", inView("graph", "Enter on the standard's chip left Graph"));

    // (3) In Graph statement 6 names the catalogue's constraint by its standard, as words alone.
    const catalogueStandard = (await lensCatalogue())[0]?.sources?.[0]?.standard ?? "";
    await expectCatalogueChip(page, ids, catalogueStandard, "graph");

    // (2) and (3) in Conversation: statement 5 names the standard by the same chip, and statement
    // 6 the catalogue's constraint by its words alone. Where Enter on the standard's chip goes
    // from Conversation waits on the filter by kind, and is the owed row's.
    await gotoIssue(page, presetPath(key, "conversation"));
    await expectOpeningLanded(page);
    const statementFive = await expectStandardChip(page, ids, lenses, "conversation");
    await expectCatalogueChip(page, ids, catalogueStandard, "conversation");

    // (4) Under Spec, statement 5 reads the same line, the standard's chip by its words, and the
    // chip, which is a link, opens Graph at the standard's row, focused.
    await gotoIssue(page, presetPath(key, "spec"));
    const specFive = nodeRow(page, statementFive).first();
    await expect(specFive, "the Spec view draws no row for statement 5").toHaveCount(1);
    await expect
      .poll(
        async () => {
          const line = lineReading(await lensLines(specFive), "constrained_by");
          return line === undefined
            ? undefined
            : { label: line.label, words: line.targets.map((target) => target.words) };
        },
        { message: "under Spec, statement 5 does not read Constrained by with the standard's chip" }
      )
      .toEqual({
        label: LENS_WORDS.constrained_by,
        words: [lenses.standard.standard ?? ""],
      });
    const specChip = lensChipLinks(specFive, "constrained_by");
    await expect(specChip, "under Spec, the standard's chip on statement 5 is no link").toHaveCount(
      1
    );
    await specChip.click();
    await expectAt(page, key, "graph", "under Spec, the standard's chip does not open Graph");
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "under Spec, the standard's chip does not open Graph at the standard's row",
        timeout: 30_000,
      })
      .toBe(ids.standard);

    // (5) `?node=` with the measurement's id on Graph's address opens Graph with its row focused.
    await gotoIssue(page, `${graph}?node=${encodeURIComponent(ids.measurement)}`);
    await expect
      .poll(() => focusedRowReadId(page), {
        message:
          "on Graph's address, ?node= with the measurement's id does not open with its row focused",
        timeout: 30_000,
      })
      .toBe(ids.measurement);
  });
});

// Conversation's own read, owed until the model's filter of the tree read by kind is on the
// base, which answers Conversation's read (`?kind=`) with the kinds it names, no constraint among
// them: titled so that no check's `-g` matches it, above all c-constraints' (`a constraint stands
// under the issue`), since no project inverts `@owed` and that run does not either.
// SLOW(>=2s): a whole seeded issue with its lenses, the gate's read of the tree by kind, a load
// of Graph and two of Conversation, every branch opened, and a chip pressed that opens Graph.
test("Conversation's own read draws no constraint row and opens Graph for one @owed", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedLensIssue, async (page, { lenses, whole }) => {
    const key = whole.key;
    owedUnless(await treeReadGate(page, key));
    owedUnless(await convertedGate(page, key));
    owedUnless(await kindFilterGate(page, key));
    const plain = presetPath(key, "conversation");
    const tree = treeAnswerOf(page);
    // Graph's read is the whole tree, the issue's constraints in it, which Conversation's is not
    // once the filter is on the base: the row names each node by the id Graph's read gives it.
    await gotoIssue(page, presetPath(key, "graph"));
    await expectOpeningLanded(page);
    const ids = lensReadIds(await heldAnswer(tree, `Graph's tree read of ${key}`), key, lenses);

    // In Conversation neither constraint is a row, once statement 5's line names the standard.
    await gotoIssue(page, plain);
    await expectOpeningLanded(page);
    const statementFive = await drawnId(page, ids.statements[4] ?? "", "statement 5");
    const fiveRow = nodeRow(page, statementFive).first();
    await expect
      .poll(
        async () =>
          lineReading(await lensLines(fiveRow), "constrained_by")?.targets.map(
            (target) => target.words
          ),
        { message: "statement 5 does not read Constrained by with the standard's chip" }
      )
      .toEqual([lenses.standard.standard ?? ""]);
    await unfoldEveryBranch(page);
    // Every branch is open, and statement 5's line came from the one read a constraint's row
    // would come from, so the rows drawn now are every row that read draws.
    const constraintRows = await Promise.all(
      [ids.standard, ids.measurement].map((id) => drawnIdOf(page, id))
    );
    expect(constraintRows, "in Conversation a constraint is a row").toEqual([null, null]);

    // Statement 5's chip in Conversation opens Graph with the standard's row focused.
    await focusRow(page, statementFive);
    await pressFTo(
      page,
      lensChipLinks(fiveRow, "constrained_by"),
      "f does not reach the standard's chip on statement 5"
    );
    await page.keyboard.press("Enter");
    await expectAt(
      page,
      key,
      "graph",
      "Enter on the standard's chip in Conversation does not open Graph"
    );
    await expect
      .poll(() => focusedRowReadId(page), {
        message: "Enter on the standard's chip in Conversation does not focus the standard's row",
        timeout: 30_000,
      })
      .toBe(ids.standard);

    // `?node=` with the measurement's id on the issue's own address opens Graph with it focused.
    await gotoIssue(page, `${plain}?node=${encodeURIComponent(ids.measurement)}`);
    await expectAt(
      page,
      key,
      "graph",
      "?node= with the measurement's id on the issue's own address does not open Graph"
    );
    await expect
      .poll(() => focusedRowReadId(page), {
        message:
          "?node= with the measurement's id on the issue's own address does not open with its row focused",
        timeout: 30_000,
      })
      .toBe(ids.measurement);
  });
});
